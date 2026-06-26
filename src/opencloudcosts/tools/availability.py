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
from opencloudcosts.utils.money import _money, _price
from opencloudcosts.utils.regions import region_display_name
from opencloudcosts.utils.spec_infer import fill_domain, spec_error_response

logger = logging.getLogger(__name__)

_SPEC_ADAPTER: TypeAdapter[PricingSpec] = TypeAdapter(PricingSpec)


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
            "regions": [{"code": r, "name": region_display_name(provider, r)} for r in regions],
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
        max_results: int = 50,
    ) -> dict[str, Any]:
        """
        List available compute instance types in a region, with optional filters.

        Useful for discovering what instances are available before fetching pricing,
        or for finding instances that meet specific vCPU/memory/GPU requirements.

        Args:
            provider: Cloud provider — "aws", "gcp", or "azure"
            region: Region code, e.g. "us-east-1" (AWS), "us-central1" (GCP), "eastus" (Azure)
            family: Optional instance family prefix, e.g. "m5", "c6g" (AWS), "n2", "c2" (GCP),
                    or "Standard_D", "Standard_E" (Azure)
            min_vcpu: Filter to instances with at least this many vCPUs.
            min_memory_gb: Filter to instances with at least this much memory (GB).
            gpu: If true, only return GPU instances
            max_results: Maximum number of results (default 50)
        """
        pvdr = ctx.request_context.lifespan_context["providers"].get(provider)
        if pvdr is None:
            return {"error": f"Provider '{provider}' not configured."}

        # Azure Retail Prices API does not expose vCPU/memory metadata.
        # Filtering by min_vcpu or min_memory_gb would silently return
        # zero-spec instances, which misleads the LLM. Return a clear error.
        if provider == "azure" and (min_vcpu is not None or min_memory_gb is not None):
            return {
                "result": "specs_unavailable",
                "provider": "azure",
                "message": (
                    "Azure vCPU/memory specs are not available via the Retail Prices API. "
                    "Use the 'family' filter with ARM SKU name prefixes instead."
                ),
                "suggestion": (
                    "Try family='Standard_D4s' (4 vCPU general purpose), "
                    "'Standard_D8s' (8 vCPU), "
                    "'Standard_E4s' (4 vCPU memory-optimised), "
                    "'Standard_F4s' (4 vCPU compute-optimised)"
                ),
                "docs": "https://learn.microsoft.com/en-us/azure/virtual-machines/sizes",
            }

        try:
            instances = await pvdr.list_instance_types(
                region=region,
                family=family or None,
                min_vcpus=min_vcpu,
                min_memory_gb=min_memory_gb,
                gpu=gpu,
            )
        except Exception as e:
            logger.error("list_instance_types error: %s", e)
            return {"error": str(e)}

        instances.sort(key=lambda i: (i.vcpu, i.memory_gb))

        # Post-call safety-net filtering: re-apply spec filters against returned
        # instances in case the provider silently ignores them (e.g. Azure).
        if min_vcpu is not None:
            instances = [i for i in instances if i.vcpu >= min_vcpu]
        if min_memory_gb is not None:
            instances = [i for i in instances if i.memory_gb >= min_memory_gb]

        total_found = len(instances)

        # Spec-filtered queries: auto-expand the cap so a broad filter like
        # "min_vcpu=4" doesn't silently drop hundreds of valid candidates.
        # The worst outcome is a missed cheaper option due to truncation.
        effective_max = max_results
        if (min_vcpu is not None or min_memory_gb is not None) and effective_max < 200:
            effective_max = 200

        truncated = total_found > effective_max
        instances = instances[:effective_max]

        # Suggest a get_prices_batch call so the LLM can immediately price the results.
        # Cap the suggested batch at 10 types to keep the follow-up call fast.
        _PRICE_BATCH_CAP = 10
        batch_types = [i.instance_type for i in instances[:_PRICE_BATCH_CAP]]
        _prices_batch_hint = (
            f'get_prices_batch(provider="{provider}", instance_types={batch_types}, '
            f'region="{region}") — price these instances sorted cheapest first'
        )
        next_steps: list[str] = []
        if truncated:
            spec_filters_applied = min_vcpu is not None or min_memory_gb is not None
            if spec_filters_applied:
                # Pick a concrete example instance type based on provider and min_vcpu
                if provider == "aws":
                    if min_vcpu is not None and min_vcpu <= 4:
                        _example_type = "m5.xlarge"
                        _example_desc = "4vCPU/16GB"
                    elif min_vcpu is not None and min_vcpu <= 8:
                        _example_type = "m5.2xlarge"
                        _example_desc = "8vCPU/32GB"
                    else:
                        _example_type = "r6i.xlarge"
                        _example_desc = "4vCPU/32GB"
                elif provider == "gcp":
                    if min_vcpu is not None and min_vcpu <= 4:
                        _example_type = "n2-standard-4"
                        _example_desc = "4vCPU/16GB"
                    else:
                        _example_type = "n2-standard-8"
                        _example_desc = "8vCPU/32GB"
                else:
                    if min_vcpu is not None and min_vcpu <= 4:
                        _example_type = "Standard_D4s_v3"
                        _example_desc = "4vCPU/16GB"
                    else:
                        _example_type = "Standard_D8s_v3"
                        _example_desc = "8vCPU/32GB"
                next_steps.append(
                    f"Spec filters applied — {total_found} total matches, showing {effective_max}. "
                    f"There are {total_found - effective_max} more instances not shown. "
                    f"To see all: re-call with max_results={total_found}. "
                    f"To narrow: add family filter (e.g. family='m5') or tighten min_vcpu/min_memory_gb. "
                    f"To price this sample immediately: call get_prices_batch with the instance_types above. "
                    f"Or go direct: get_compute_price(provider='{provider}', "
                    f"instance_type='{_example_type}', region='{region}') for a known {_example_desc} type."
                )
            else:
                # No spec filters — guide the LLM to narrow by family first
                if provider == "aws":
                    _example_family = family if family else "m5"
                    _family_hint = (
                        f'family="{_example_family}" (AWS general-purpose) or '
                        f'"c6g" (ARM compute-optimised) or "r6g" (memory-optimised)'
                    )
                elif provider == "gcp":
                    _example_family = family if family else "n2-standard"
                    _family_hint = (
                        f'family="{_example_family}" (GCP general-purpose) or '
                        f'"c2-standard" (compute-optimised) or "m2-ultramem" (memory-optimised)'
                    )
                else:
                    _example_family = family if family else "Standard_D"
                    _family_hint = f'family="{_example_family}"'
                next_steps.append(
                    f"Result truncated: returned {effective_max} of {total_found} matches. "
                    f"Narrow by family — e.g. {_family_hint} — or add "
                    f"min_vcpu/min_memory_gb spec filters. "
                    f"To retrieve all {total_found} results: re-call with max_results={total_found}."
                )
                next_steps.append(_prices_batch_hint)
        else:
            next_steps.append(_prices_batch_hint)

        response: dict[str, Any] = {
            "provider": provider,
            "region": region,
            "region_name": region_display_name(provider, region),
            "filters": {
                "family": family or None,
                "min_vcpu": min_vcpu,
                "min_memory_gb": min_memory_gb,
                "gpu": gpu,
            },
            "count": len(instances),
            "total_found": total_found,
            "truncated": truncated,
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
            "next_steps": next_steps,
        }
        if provider == "azure":
            response["specs_note"] = (
                "Azure does not expose vCPU/memory specs via the Retail Prices API. "
                "Filter by family name prefix instead "
                "(e.g. family='Standard_D4s' for 4-vCPU D-series). "
                "See https://learn.microsoft.com/en-us/azure/virtual-machines/sizes for specs."
            )
        return response

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
        example = cat.example_invocations.get(svc_key) or cat.example_invocations.get(domain)
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
            base_spec = _SPEC_ADAPTER.validate_python(fill_domain(spec))
        except Exception as e:
            return spec_error_response(e, spec)

        pvdr = ctx.request_context.lifespan_context["providers"].get(base_spec.provider.value)
        if pvdr is None:
            return {"error": f"Provider '{base_spec.provider.value}' not configured."}

        provider_str = base_spec.provider.value
        all_regions_requested = regions == ["all"]

        if not regions or all_regions_requested:
            all_available = await pvdr.list_regions("compute")
            major = pvdr.major_regions()
            if major and not all_regions_requested:
                regions = major
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
            return {"result": "no_prices_found", "message": "No pricing found in any region."}

        all_prices.sort(key=lambda p: p.price_per_unit)

        entries = []
        for p in all_prices:
            entry: dict[str, Any] = {
                "region": p.region,
                "region_name": region_display_name(provider_str, p.region),
                "price_per_unit": _price(p.price_per_unit, p.unit.value),
            }
            if p.unit.value in ("per_hour", "per_month"):
                entry["monthly_estimate"] = _money(p.monthly_cost, "/mo")
            entries.append(entry)

        if baseline_region:
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
            "cheapest_region_name": region_display_name(provider_str, cheapest.region),
            "cheapest_price": _price(cheapest.price_per_unit, cheapest.unit.value),
            "most_expensive_region": most_exp.region,
            "most_expensive_price": _price(most_exp.price_per_unit, most_exp.unit.value),
            "price_delta_pct": (
                round(
                    float(
                        (most_exp.price_per_unit - cheapest.price_per_unit)
                        / cheapest.price_per_unit
                        * 100
                    ),
                    1,
                )
                if cheapest.price_per_unit > 0
                else None
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
            base_spec = _SPEC_ADAPTER.validate_python(fill_domain(spec))
        except Exception as e:
            return spec_error_response(e, spec)

        pvdr = ctx.request_context.lifespan_context["providers"].get(base_spec.provider.value)
        if pvdr is None:
            return {"error": f"Provider '{base_spec.provider.value}' not configured."}

        provider_str = base_spec.provider.value
        all_regions_requested = regions == ["all"]

        if not regions or all_regions_requested:
            all_available = await pvdr.list_regions("compute")
            major = pvdr.major_regions()
            if major and not all_regions_requested:
                regions = major
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
            return {
                "result": "not_available",
                "message": "Not available in any of the checked regions.",
            }

        available.sort(key=lambda x: x[1].price_per_unit)

        entries = []
        for region, p in available:
            entry: dict[str, Any] = {
                "region": region,
                "region_name": region_display_name(provider_str, region),
                "price_per_unit": _price(p.price_per_unit, p.unit.value),
            }
            if p.unit.value in ("per_hour", "per_month"):
                entry["monthly_estimate"] = _money(p.monthly_cost, "/mo")
            entries.append(entry)

        if baseline_region:
            try:
                apply_baseline_deltas(entries, baseline_region, hourly_key="price_per_unit")
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
